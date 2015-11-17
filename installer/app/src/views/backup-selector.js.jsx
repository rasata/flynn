import Dispatcher from '../dispatcher';
import FileInput from './file-input';

var BackupSelector = React.createClass({
	render: function () {
		return (
			<div style={this.props.style}>
				<br/>
				<label>
					<span>Restore from backup:</span>
					<FileInput onChange={this.__handleFileChange} style={{
						maxWidth: 400
					}} />
				</label>
			</div>
		);
	},

	__handleFileChange: function (file) {
		Dispatcher.dispatch({
			clusterID: 'new',
			name: 'SELECT_BACKUP',
			file: file
		});
	}
});

export default BackupSelector;
